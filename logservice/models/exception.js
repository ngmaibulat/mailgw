'use strict';
const {
  Model
} = require('sequelize');
module.exports = (sequelize, DataTypes) => {
  class Exception extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
      // define association here
    }
  }
  Exception.init({
    product: DataTypes.STRING,
    component: DataTypes.STRING,
    info: DataTypes.TEXT
  }, {
    sequelize,
    modelName: 'Exception',
  });
  return Exception;
};